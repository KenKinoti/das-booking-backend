package subscriptions

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/currency"
)

// LineInput is one line of a subscription.
type LineInput struct {
	ServiceID   string   `json:"service_id"`
	Kind        string   `json:"kind"`
	Description string   `json:"description"`
	Quantity    float64  `json:"quantity"`
	UnitPrice   *float64 `json:"unit_price"` // null = follow the catalogue service's price
	TaxRate     *float64 `json:"tax_rate"`   // null = the invoicing default
}

// SubInput creates or updates a subscription.
type SubInput struct {
	CustomerID       string      `json:"customer_id"`
	Name             string      `json:"name"`
	Currency         string      `json:"currency"`
	Period           string      `json:"period"`
	StartDate        string      `json:"start_date"`
	BillingDay       int         `json:"billing_day"`
	Prorate          bool        `json:"prorate"`
	PaymentTermsDays *int        `json:"payment_terms_days"`
	LeadDays         *int        `json:"lead_days"`
	AutoSend         *bool       `json:"auto_send"`
	GraceDays        *int        `json:"grace_days"`
	AutoSuspend      *bool       `json:"auto_suspend"`
	EndDate          string      `json:"end_date"`
	Lines            []LineInput `json:"lines"`
	SiteDomain       string      `json:"site_domain"`
	Domain           string      `json:"domain"`
	Domains          []string    `json:"domains"`
	Supplier         string      `json:"supplier"`
	SupplierPlan     string      `json:"supplier_plan"`
	DomainExpiry     string      `json:"domain_expiry"`
	Notes            string      `json:"notes"`
	CostMonthly      *float64    `json:"cost_monthly"`
	Source           string      `json:"source"`
	// BillPastPeriods: invoice the periods that started before today too.
	// false starts billing at the next renewal (earlier periods were billed
	// elsewhere — an existing client); nil decides by itself: from the start
	// when it is at most 7 days ago, else from the next renewal.
	BillPastPeriods *bool `json:"bill_past_periods"`
}

func validSupplier(s string) bool {
	switch s {
	case SupplierSynergy, SupplierHostingCom, SupplierOther, SupplierNone:
		return true
	}
	return false
}

// SupplierLabel is a supplier key in words.
func SupplierLabel(s string) string {
	switch s {
	case SupplierSynergy:
		return "Synergy Wholesale"
	case SupplierHostingCom:
		return "hosting.com"
	case SupplierOther:
		return "the provider"
	}
	return ""
}

func cleanDomain(d string) string {
	d = strings.ToLower(strings.TrimSpace(d))
	d = strings.TrimPrefix(strings.TrimPrefix(d, "https://"), "http://")
	d = strings.TrimPrefix(d, "www.")
	if i := strings.IndexAny(d, "/?# "); i >= 0 {
		d = d[:i]
	}
	return clip(strings.TrimSuffix(d, "."), 250)
}

func validDomain(d string) bool {
	if d == "" || len(d) > 250 || !strings.Contains(d, ".") {
		return false
	}
	for _, r := range d {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '.' || r > 127) {
			return false
		}
	}
	return !strings.HasPrefix(d, ".") && !strings.Contains(d, "..")
}

// apply validates in onto sub; locked = periods were invoiced (the schedule
// can no longer change).
func (s *Service) apply(org string, sub *Subscription, in SubInput, locked bool) ([]Line, error) {
	cu, ok := s.customer(org, strings.TrimSpace(in.CustomerID))
	if !ok {
		return nil, invalid("choose a customer")
	}
	if locked && cu.ID != sub.CustomerID {
		return nil, invalid("the customer can't change once the subscription has invoices — cancel it and create a new one")
	}
	period := strings.TrimSpace(in.Period)
	if period == "" {
		period = PeriodMonthly
	}
	if PeriodMonths(period) == 0 {
		return nil, invalid("choose a billing period: monthly, quarterly or yearly")
	}
	today := s.Today(org)
	start := today
	if strings.TrimSpace(in.StartDate) != "" {
		d, ok := ParseDay(in.StartDate)
		if !ok {
			return nil, invalid("the start date must be YYYY-MM-DD")
		}
		start = d
	}
	if start.Before(today.AddDate(-5, 0, 0)) || start.After(today.AddDate(2, 0, 0)) {
		return nil, invalid("the start date must be within 5 years ago and 2 years ahead")
	}
	if in.BillingDay < 0 || in.BillingDay > 31 {
		return nil, invalid("the billing day is 1 to 31")
	}
	sched, err := NewSchedule(start, in.BillingDay, in.Prorate, period)
	if err != nil {
		return nil, invalid("%s", err.Error())
	}
	if locked {
		same := sub.Period == period && Day(sub.StartDate).Equal(start) && sub.BillingDay == sched.BillingDay && sub.Prorate == in.Prorate
		if !same {
			return nil, invalid("the billing period, start date, billing day and proration can't change once invoices were created")
		}
	}
	home := s.home(org)
	cur := strings.ToUpper(strings.TrimSpace(in.Currency))
	if cur == "" {
		if locked {
			cur = sub.Currency
		} else {
			cur, _ = currency.ForCustomer(cu.Currency, cu.CountryCode, cu.Country, home)
		}
	}
	if !currency.IsSupported(cur) {
		return nil, invalid("choose a supported currency")
	}
	if locked && cur != sub.Currency {
		return nil, invalid("the currency can't change once invoices were created")
	}
	terms := 0
	if in.PaymentTermsDays != nil {
		terms = *in.PaymentTermsDays
	} else if locked {
		terms = sub.PaymentTermsDays
	} else if cu.Terms != nil && *cu.Terms > 0 {
		terms = *cu.Terms
	}
	if terms < 0 || terms > 90 {
		return nil, invalid("payment terms are 0 to 90 days after the period starts")
	}
	if in.LeadDays != nil && (*in.LeadDays < 0 || *in.LeadDays > 90) {
		return nil, invalid("create invoices 0 to 90 days before the period starts")
	}
	if in.GraceDays != nil && (*in.GraceDays < 0 || *in.GraceDays > 60) {
		return nil, invalid("the grace period is 0 to 60 days after the due date")
	}
	var end *time.Time
	if strings.TrimSpace(in.EndDate) != "" {
		d, ok := ParseDay(in.EndDate)
		if !ok {
			return nil, invalid("the end date must be YYYY-MM-DD")
		}
		if d.Before(start) {
			return nil, invalid("the end date must be after the start date")
		}
		end = &d
	}
	var dexp *time.Time
	if strings.TrimSpace(in.DomainExpiry) != "" {
		d, ok := ParseDay(in.DomainExpiry)
		if !ok {
			return nil, invalid("the domain expiry must be YYYY-MM-DD")
		}
		dexp = &d
	}
	sup := strings.TrimSpace(in.Supplier)
	if !validSupplier(sup) {
		return nil, invalid("choose where it is hosted: Synergy Wholesale, hosting.com or another provider")
	}
	if in.CostMonthly != nil && (*in.CostMonthly < 0 || *in.CostMonthly > 1e9) {
		return nil, invalid("the monthly cost must be zero or more")
	}
	site, dom := cleanDomain(in.SiteDomain), cleanDomain(in.Domain)
	for _, d := range []string{site, dom} {
		if d != "" && !validDomain(d) {
			return nil, invalid("%q is not a domain name (e.g. wanjiku.co.ke)", d)
		}
	}
	more := []string{}
	seen := map[string]bool{site: true, dom: true}
	for _, d := range in.Domains {
		d = cleanDomain(d)
		if d == "" || seen[d] {
			continue
		}
		if !validDomain(d) {
			return nil, invalid("%q is not a domain name (e.g. wanjiku.co.ke)", d)
		}
		seen[d] = true
		more = append(more, d)
	}
	if len(more) > 10 {
		return nil, invalid("link at most 10 more sites or domains")
	}
	if sup == "" && site != "" && s.isSynergySite(org, site) {
		sup = SupplierSynergy
	}
	if len(in.Lines) == 0 {
		return nil, invalid("add at least one service")
	}
	if len(in.Lines) > 20 {
		return nil, invalid("add at most 20 services")
	}
	ids := []string{}
	for _, li := range in.Lines {
		if id := strings.TrimSpace(li.ServiceID); id != "" {
			ids = append(ids, id)
		}
	}
	cat := s.catalog(org, ids)
	tax := s.taxDefaults(org)
	lines := []Line{}
	names := []string{}
	for i, li := range in.Lines {
		l := Line{ID: uuid.NewString(), OrganizationID: org, Position: i, Quantity: li.Quantity, Description: clip(li.Description, 480), Kind: strings.TrimSpace(li.Kind), TaxRate: tax.Rate}
		if l.Quantity == 0 {
			l.Quantity = 1
		}
		if l.Quantity < 0 || l.Quantity > 10000 {
			return nil, invalid("line %d: the quantity must be more than zero", i+1)
		}
		if li.TaxRate != nil {
			if *li.TaxRate < 0 || *li.TaxRate > 100 {
				return nil, invalid("line %d: the tax rate must be between 0 and 100", i+1)
			}
			l.TaxRate = *li.TaxRate
		}
		if li.UnitPrice != nil {
			if *li.UnitPrice < 0 || *li.UnitPrice > 1e9 || math.IsNaN(*li.UnitPrice) {
				return nil, invalid("line %d: the price must be zero or more", i+1)
			}
			v := RoundCur(*li.UnitPrice, cur)
			l.UnitPrice = &v
		}
		if id := strings.TrimSpace(li.ServiceID); id != "" {
			svc, ok := cat[id]
			if !ok {
				return nil, invalid("line %d: the catalogue service was not found", i+1)
			}
			if !svc.Active {
				var had int64
				if sub.ID != "" {
					s.DB.Model(&Line{}).Where("subscription_id = ? AND service_id = ?", sub.ID, id).Count(&had)
				}
				if had == 0 {
					return nil, invalid("line %d: %s is no longer active in the catalogue", i+1, svc.Name)
				}
			}
			l.ServiceID = id
			if l.Kind == "" {
				l.Kind = svc.Kind
			}
			if l.Description == "" {
				l.Description = clip(svc.Label, 480)
			}
		} else {
			if l.Description == "" {
				return nil, invalid("line %d: choose a service or describe the item", i+1)
			}
			if l.UnitPrice == nil {
				return nil, invalid("line %d: enter the price", i+1)
			}
			if l.Kind == "" {
				l.Kind = InferKind(l.Description, "")
			}
		}
		if !IsKind(l.Kind) {
			l.Kind = KindOther
		}
		names = append(names, l.Description)
		lines = append(lines, l)
	}
	name := strings.Join(strings.Fields(in.Name), " ")
	if name == "" {
		name = strings.Join(names, " + ")
		if w := firstNonEmpty(site, dom); w != "" {
			name += " — " + w
		}
	}
	sub.CustomerID, sub.Name, sub.Currency, sub.Period = cu.ID, clip(name, 250), cur, period
	sub.StartDate, sub.BillingDay, sub.Prorate, sub.AnchorDate = start, sched.BillingDay, in.Prorate, sched.Anchor
	sub.PaymentTermsDays, sub.LeadDays, sub.GraceDays, sub.EndDate = terms, in.LeadDays, in.GraceDays, end
	st := s.SettingsFor(org)
	if in.AutoSend != nil {
		sub.AutoSend = *in.AutoSend
	} else if sub.Number == "" { // a new subscription: the organisation's default
		sub.AutoSend = st.AutoSend
	}
	if in.AutoSuspend != nil {
		sub.AutoSuspend = *in.AutoSuspend
	} else if sub.Number == "" {
		sub.AutoSuspend = st.AutoSuspend
	}
	sub.SiteDomain, sub.Domain, sub.Domains, sub.Supplier, sub.SupplierPlan = site, dom, more, sup, clip(in.SupplierPlan, 190)
	sub.DomainExpiry, sub.Notes, sub.CostMonthly = dexp, strings.TrimSpace(in.Notes), in.CostMonthly
	if !locked {
		sp := sched.First()
		past := sp.Start.Before(today) && !sp.Start.Before(today.AddDate(0, 0, -7))
		if in.BillPastPeriods != nil {
			past = *in.BillPastPeriods
		}
		sub.PaidThrough = nil
		if !past && sp.Start.Before(today) {
			for i := 0; i < 2000 && sp.Start.Before(today); i++ {
				sp = sched.Next(sp)
			}
			pt := sp.Start.AddDate(0, 0, -1)
			sub.PaidThrough = &pt // billed before (another system)
		}
		sub.NextPeriodStart = sp.Start
		sub.NextBillingDate = sp.Start
	}
	return lines, nil
}

// Create stores a new subscription. Its first invoice is created by the
// next engine run (straight away when it is already due).
func (s *Service) Create(org, user string, in SubInput) (*Subscription, error) {
	sub := &Subscription{ID: uuid.NewString(), OrganizationID: org, CreatedBy: user, CreatedAt: s.now(), UpdatedAt: s.now(), Source: clip(in.Source, 20), Domains: []string{}}
	lines, err := s.apply(org, sub, in, false)
	if err != nil {
		return nil, err
	}
	sub.Status = StatusActive
	err = s.DB.Transaction(func(tx *gorm.DB) error {
		num, err := s.nextNumber(tx, org)
		if err != nil {
			return err
		}
		sub.Number = num
		for i := range lines {
			lines[i].SubscriptionID = sub.ID
		}
		if err := tx.Omit("Lines").Create(sub).Error; err != nil {
			return err
		}
		return tx.Create(&lines).Error
	})
	if err != nil {
		return nil, err
	}
	s.record(org, Event{Key: sub.ID + "|created", SubscriptionID: sub.ID, Stage: "created", Audience: ToAdmin, Day: s.Today(org), CreatedBy: user,
		Subject: "Subscription created" + map[bool]string{true: " from " + sourceLabel(sub.Source), false: ""}[sub.Source != ""]})
	return s.Get(org, sub.ID)
}

func sourceLabel(src string) string {
	switch src {
	case "synergy":
		return "a Synergy Wholesale site"
	case "csv":
		return "a CSV import"
	}
	return src
}

// Update changes a subscription.
func (s *Service) Update(org, id string, in SubInput) (*Subscription, error) {
	sub, err := s.load(org, id)
	if err != nil {
		return nil, err
	}
	var invoiced int64
	s.DB.Model(&BillingPeriod{}).Where("subscription_id = ?", id).Count(&invoiced)
	wasAuto := sub.AutoSuspend
	lines, err := s.apply(org, sub, in, invoiced > 0)
	if err != nil {
		return nil, err
	}
	sub.UpdatedAt = s.now()
	err = s.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Omit("Lines").Save(sub).Error; err != nil {
			return err
		}
		if err := tx.Where("subscription_id = ?", id).Delete(&Line{}).Error; err != nil {
			return err
		}
		for i := range lines {
			lines[i].SubscriptionID = id
		}
		return tx.Create(&lines).Error
	})
	if err != nil {
		return nil, err
	}
	s.refreshNextBilling(id)
	if !wasAuto && sub.AutoSuspend {
		s.ensureNotice(sub, s.SettingsFor(org), s.Today(org))
	}
	return s.Get(org, id)
}

// Delete removes a subscription that never had an invoice.
func (s *Service) Delete(org, id string) error {
	if _, err := s.load(org, id); err != nil {
		return err
	}
	var n int64
	s.DB.Model(&BillingPeriod{}).Where("subscription_id = ? AND invoice_id <> ''", id).Count(&n)
	if n > 0 {
		return invalid("this subscription has invoices — cancel it instead")
	}
	return s.DB.Transaction(func(tx *gorm.DB) error {
		for _, m := range []any{&Line{}, &BillingPeriod{}, &Task{}, &Event{}} {
			if err := tx.Where("subscription_id = ?", id).Delete(m).Error; err != nil {
				return err
			}
		}
		return tx.Where("organization_id = ? AND id = ?", org, id).Delete(&Subscription{}).Error
	})
}

func (s *Service) load(org, id string) (*Subscription, error) {
	var sub Subscription
	if err := s.DB.Preload("Lines", func(db *gorm.DB) *gorm.DB { return db.Order("position") }).Where("organization_id = ? AND id = ?", org, id).First(&sub).Error; err != nil {
		return nil, ErrNotFound
	}
	if sub.Domains == nil {
		sub.Domains = []string{}
	}
	return &sub, nil
}

// Schedule of a subscription.
func (sub *Subscription) Schedule() Schedule {
	sc, _ := NewSchedule(sub.StartDate, sub.BillingDay, sub.Prorate, sub.Period)
	if !sub.AnchorDate.IsZero() {
		sc.Anchor = Day(sub.AnchorDate)
	}
	return sc
}

// HasKind reports whether a line is of kind k.
func (sub *Subscription) HasKind(k string) bool {
	for _, l := range sub.Lines {
		if l.Kind == k {
			return true
		}
	}
	return false
}

// AllDomains are the site, the domain and the extra ones (unique, in order).
func (sub *Subscription) AllDomains() []string {
	out := []string{}
	seen := map[string]bool{}
	for _, d := range append([]string{sub.SiteDomain, sub.Domain}, sub.Domains...) {
		if d != "" && !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	return out
}

// graceFor is the subscription's grace period (its own, else the organisation's).
func graceFor(sub *Subscription, st Settings) int {
	if sub.GraceDays != nil {
		return *sub.GraceDays
	}
	return st.GraceDays
}

// datesFor works out an unpaid invoice's dunning dates.
func (s *Service) datesFor(sub *Subscription, per *BillingPeriod, st Settings, warned *time.Time, today time.Time) Dates {
	return PolicyOf(st).DatesFor(per.DueDate, graceFor(sub, st), per.ExtraDays, sub.AutoSuspend && !per.NoSuspend, warned, today)
}

// warnedOn is the first day the admins were alerted about each period's suspension.
func (s *Service) warnedOn(periodIDs []string) map[string]time.Time {
	out := map[string]time.Time{}
	if len(periodIDs) == 0 {
		return out
	}
	var rows []struct {
		PeriodID string
		Day      time.Time
	}
	s.DB.Raw(`SELECT period_id, MIN(day) AS day FROM client_subscription_events WHERE period_id IN ? AND stage LIKE 'admin\_%' AND audience = ?
		AND (status = 'sent' OR in_app > 0) GROUP BY period_id`, periodIDs, ToAdmin).Scan(&rows)
	for _, r := range rows {
		out[r.PeriodID] = Day(r.Day)
	}
	return out
}

func (s *Service) warnedFor(periodID string) *time.Time {
	if t, ok := s.warnedOn([]string{periodID})[periodID]; ok {
		return &t
	}
	return nil
}

// refreshNextBilling stores the start of the earliest unpaid period (else
// the next period to invoice).
func (s *Service) refreshNextBilling(subID string) {
	var sub Subscription
	if s.DB.Select("id, next_period_start, next_billing_date").Where("id = ?", subID).First(&sub).Error != nil {
		return
	}
	next := Day(sub.NextPeriodStart)
	var open []time.Time
	s.DB.Raw(`SELECT period_start FROM client_subscription_periods WHERE subscription_id = ? AND status IN ? ORDER BY period_start LIMIT 1`,
		subID, []string{PerInvoiced, PerGenerating}).Scan(&open)
	if len(open) > 0 && Day(open[0]).Before(next) {
		next = Day(open[0])
	}
	if !Day(sub.NextBillingDate).Equal(next) {
		s.DB.Model(&Subscription{}).Where("id = ?", subID).Update("next_billing_date", next)
	}
}

// ---------------------------------------------------------------- read

// Filter narrows the list.
type Filter struct {
	Status     string
	CustomerID string
	Period     string
	Supplier   string
	Q          string
}

// List lists subscriptions with their amounts and dunning state.
func (s *Service) List(org string, f Filter) []Subscription {
	out := []Subscription{}
	q := s.DB.Preload("Lines", func(db *gorm.DB) *gorm.DB { return db.Order("position") }).Where("organization_id = ?", org)
	switch f.Status {
	case "":
	case "open":
		q = q.Where("status <> ?", StatusCancelled)
	case "at_risk":
		q = q.Where("status IN ?", []string{StatusPastDue, StatusSuspended})
	default:
		q = q.Where("status = ?", f.Status)
	}
	if f.CustomerID != "" {
		q = q.Where("customer_id = ?", f.CustomerID)
	}
	if f.Period != "" {
		q = q.Where("period = ?", f.Period)
	}
	if f.Supplier != "" {
		if f.Supplier == "none" {
			q = q.Where("COALESCE(supplier, '') = ''")
		} else {
			q = q.Where("supplier = ?", f.Supplier)
		}
	}
	q.Order("created_at DESC").Limit(3000).Find(&out)
	s.decorate(org, out)
	if t := strings.ToLower(strings.TrimSpace(f.Q)); t != "" {
		kept := out[:0]
		for _, x := range out {
			parts := []string{x.Number, x.Name, x.CustomerName, x.CustomerEmail, x.SiteDomain, x.Domain, x.SupplierPlan}
			parts = append(parts, x.Domains...)
			for _, l := range x.Lines {
				parts = append(parts, l.Description)
			}
			if strings.Contains(strings.ToLower(strings.Join(parts, " ")), t) {
				kept = append(kept, x)
			}
		}
		out = kept
	}
	return out
}

// Get returns one subscription.
func (s *Service) Get(org, id string) (*Subscription, error) {
	sub, err := s.load(org, id)
	if err != nil {
		return nil, err
	}
	l := []Subscription{*sub}
	s.decorate(org, l)
	return &l[0], nil
}

// Detail is a subscription with its invoices, tasks, events and timeline.
type Detail struct {
	*Subscription
	Periods  []BillingPeriod `json:"periods"`
	Tasks    []Task          `json:"tasks"`
	Events   []Event         `json:"events"`
	Timeline []TimelineItem  `json:"timeline"`
	Domains  []DomainInfo    `json:"domain_info"`
	Today    string          `json:"today"`
	Portal   string          `json:"portal_url,omitempty"`
}

// GetDetail returns the subscription drawer data.
func (s *Service) GetDetail(org, id string) (*Detail, error) {
	sub, err := s.Get(org, id)
	if err != nil {
		return nil, err
	}
	today := s.Today(org)
	d := &Detail{Subscription: sub, Periods: []BillingPeriod{}, Tasks: []Task{}, Events: []Event{}, Today: DayStr(today)}
	s.DB.Where("subscription_id = ?", id).Order("period_start DESC").Limit(120).Find(&d.Periods)
	s.DB.Where("subscription_id = ?", id).Order("created_at DESC").Limit(100).Find(&d.Tasks)
	s.DB.Where("subscription_id = ?", id).Order("created_at DESC").Limit(300).Find(&d.Events)
	d.Portal = s.PortalURL(org, sub.CustomerID)
	d.Timeline = s.timeline(org, sub, d.Periods, d.Events, d.Tasks, today)
	d.Domains = s.domainInfo(org, sub, today)
	return d, nil
}

type lineCalc struct {
	price       float64 // per period, per unit, in cur
	cur         string
	fromCatalog bool
	svc         *CatalogService
}

// lineCalc is a line's price per period: its own, else the catalogue
// service's (for the period, tax-inclusive or not as invoices are written).
func (s *Service) lineCalc(l *Line, cat map[string]CatalogService, period, subCur string, tax TaxDefaults) (lineCalc, bool) {
	lc := lineCalc{}
	if svc, ok := cat[l.ServiceID]; ok {
		lc.svc = &svc
	}
	if l.UnitPrice != nil {
		lc.price, lc.cur = *l.UnitPrice, subCur
		return lc, true
	}
	if lc.svc == nil {
		return lc, false
	}
	p := PerPeriod(lc.svc.Price, lc.svc.Period, period)
	switch {
	case lc.svc.PriceIncludesTax && !tax.PricesIncludeTax:
		p = p / (1 + l.TaxRate/100)
	case !lc.svc.PriceIncludesTax && tax.PricesIncludeTax:
		p = p * (1 + l.TaxRate/100)
	}
	lc.price, lc.cur, lc.fromCatalog = RoundCur(p, lc.svc.Currency), lc.svc.Currency, true
	return lc, true
}

// amounts is one period's amount before and after tax in the subscription
// currency (catalogue prices in another currency converted at today's rate).
func (s *Service) amounts(org string, sub *Subscription, cat map[string]CatalogService, tax TaxDefaults) (net, gross float64, okRate bool) {
	okRate = true
	for i := range sub.Lines {
		l := &sub.Lines[i]
		lc, ok := s.lineCalc(l, cat, sub.Period, sub.Currency, tax)
		if lc.svc != nil {
			l.ServiceName = lc.svc.Name
		}
		if !ok {
			l.Missing = true
			continue
		}
		l.Price, l.PriceCurrency, l.FromCatalog = lc.price, lc.cur, lc.fromCatalog
		v := lc.price * l.Quantity
		if lc.cur != sub.Currency {
			c, ok := s.convert(org, v, lc.cur, sub.Currency)
			if !ok {
				okRate = false
				continue
			}
			v = c
		}
		n, g := v, v*(1+l.TaxRate/100)
		if tax.PricesIncludeTax {
			n, g = v/(1+l.TaxRate/100), v
		}
		net += n
		gross += g
	}
	return RoundCur(net, sub.Currency), RoundCur(gross, sub.Currency), okRate
}

// siteCosts is what each Synergy hosted site costs per month (home
// currency), from the Expenses → Synergy report.
func (s *Service) siteCosts(org string) map[string]float64 {
	out := map[string]float64{}
	for _, st := range s.synergySites(org) {
		out[strings.ToLower(st.Domain)] = st.CostHome
	}
	return out
}

func (s *Service) isSynergySite(org, domain string) bool {
	for _, st := range s.synergySites(org) {
		if strings.EqualFold(st.Domain, domain) {
			return true
		}
	}
	return false
}

func (s *Service) decorate(org string, list []Subscription) {
	if len(list) == 0 {
		return
	}
	home := s.home(org)
	st := s.SettingsFor(org)
	today := s.Today(org)
	tax := s.taxDefaults(org)
	ids, subIDs, svcIDs := []string{}, []string{}, []string{}
	for _, x := range list {
		ids = append(ids, x.CustomerID)
		subIDs = append(subIDs, x.ID)
		for _, l := range x.Lines {
			if l.ServiceID != "" {
				svcIDs = append(svcIDs, l.ServiceID)
			}
		}
	}
	cust := s.customers(org, ids)
	cat := s.catalog(org, svcIDs)
	open, counts := s.openPeriods(org, subIDs)
	pids := []string{}
	for _, p := range open {
		pids = append(pids, p.ID)
	}
	warned := s.warnedOn(pids)
	tasks := map[string]int{}
	var tr []struct {
		SubscriptionID string
		N              int
	}
	s.DB.Raw(`SELECT subscription_id, COUNT(*) AS n FROM client_subscription_tasks WHERE organization_id = ? AND status = ? GROUP BY subscription_id`, org, TaskOpen).Scan(&tr)
	for _, t := range tr {
		tasks[t.SubscriptionID] = t.N
	}
	var sites map[string]float64
	for _, x := range list {
		if x.SiteDomain != "" && x.CostMonthly == nil {
			sites = s.siteCosts(org)
			break
		}
	}
	for i := range list {
		sub := &list[i]
		if sub.Domains == nil {
			sub.Domains = []string{}
		}
		c := cust[sub.CustomerID]
		sub.CustomerName, sub.CustomerEmail, sub.CustomerPhone = firstNonEmpty(c.Name, "Deleted customer"), c.Email, c.Phone
		sub.HomeCurrency = home
		net, gross, okRate := s.amounts(org, sub, cat, tax)
		sub.Amount, sub.AmountTax = net, gross
		if rate := s.rate(org, sub.Currency, home); rate > 0 && okRate {
			v := RoundCur(Monthly(net, sub.Period)*rate, home)
			sub.MonthlyHome = &v
		}
		sc := sub.Schedule()
		sub.Lead = Lead(sub.LeadDays, st, sub.Period)
		sub.Grace = graceFor(sub, st)
		if sub.Status != StatusCancelled {
			if sp, ok := sc.StartingOn(sub.NextPeriodStart); ok && (sub.EndDate == nil || sp.Start.Before(Day(*sub.EndDate))) {
				sub.NextDueDate = DayStr(DueDate(sp, sub.PaymentTermsDays))
				sub.NextInvoiceOn = DayStr(IssueDate(sp, sub.Lead))
			}
		}
		if op, ok := open[sub.ID]; ok {
			var w *time.Time
			if t, ok := warned[op.ID]; ok {
				w = &t
			}
			sub.Open = s.openView(op, s.datesFor(sub, &op, st, w, today), today)
			sub.Open.Periods = counts[sub.ID]
		}
		sub.OpenTasks = tasks[sub.ID]
		// Cost: the subscription's own figure, else the hosted sites' (Synergy charges).
		cost := CostView{Currency: home, Source: "none"}
		switch {
		case sub.CostMonthly != nil:
			cost.Source, cost.Monthly = "manual", *sub.CostMonthly
		case sub.SiteDomain != "":
			for _, dmn := range sub.AllDomains() {
				if v := sites[strings.ToLower(dmn)]; v > 0 {
					cost.Source = "synergy"
					cost.Monthly += v
				}
			}
			cost.Monthly = RoundCur(cost.Monthly, home)
		}
		sub.Cost = &cost
		if sub.MonthlyHome != nil && cost.Source != "none" {
			m := RoundCur(*sub.MonthlyHome-cost.Monthly, home)
			sub.Margin = &m
			if *sub.MonthlyHome > 0 {
				p := math.Round(m / *sub.MonthlyHome * 1000) / 10
				sub.MarginPct = &p
			}
		}
		sub.Summary = fmt.Sprintf("%s · %s", PeriodLabel(sub.Period), money(sub.AmountTax, sub.Currency))
	}
}

// CostView is what a subscription costs per month.
type CostView struct {
	Monthly  float64 `json:"monthly"`  // home currency
	Currency string  `json:"currency"` // home currency
	Source   string  `json:"source"`   // synergy | manual | none
}

// openPeriods are each subscription's oldest unpaid (sent or draft) period
// and how many unpaid periods it has.
func (s *Service) openPeriods(org string, subIDs []string) (map[string]BillingPeriod, map[string]int) {
	out, n := map[string]BillingPeriod{}, map[string]int{}
	if len(subIDs) == 0 {
		return out, n
	}
	var list []BillingPeriod
	s.DB.Where("organization_id = ? AND subscription_id IN ? AND status = ?", org, subIDs, PerInvoiced).Order("due_date ASC, period_start ASC").Find(&list)
	for _, p := range list {
		n[p.SubscriptionID]++
		if _, ok := out[p.SubscriptionID]; !ok {
			out[p.SubscriptionID] = p
		}
	}
	return out, n
}

func (s *Service) openView(p BillingPeriod, d Dates, today time.Time) *OpenPeriod {
	o := &OpenPeriod{PeriodID: p.ID, InvoiceID: p.InvoiceID, InvoiceNumber: p.InvoiceNumber, DueDate: DayStr(p.DueDate), Balance: p.Balance, Currency: p.Currency,
		ExtraDays: p.ExtraDays, NoSuspend: p.NoSuspend, Sent: p.Sent, Draft: !p.Sent, Deferred: d.Deferred}
	if d.Suspend != nil {
		o.SuspendOn = DayStr(*d.Suspend)
		n := DaysBetween(today, *d.Suspend)
		o.DaysToSuspend = &n
	}
	if d.Planned != nil {
		o.PlannedOn = DayStr(*d.Planned)
	}
	if d.FinalNotice != nil {
		o.FinalNoticeOn = DayStr(*d.FinalNotice)
	}
	if od := DaysBetween(p.DueDate, today); od > 0 {
		o.DaysOverdue = od
	}
	return o
}

// ---------------------------------------------------------------- timeline

// TimelineItem is one entry of a subscription's history and plan: invoices,
// payments, reminders, notices, alerts, suspensions and admin actions, with
// the planned steps of the unpaid invoice.
type TimelineItem struct {
	Date      string `json:"date"`
	Kind      string `json:"kind"`     // invoice | paid | reminder | notice | alert | suspend | unsuspend | action | provider | task | domain | next
	Audience  string `json:"audience"` // client | admin | ""
	Title     string `json:"title"`
	Detail    string `json:"detail,omitempty"`
	Status    string `json:"status"` // done | sent | failed | skipped | planned | open
	InvoiceID string `json:"invoice_id,omitempty"`
	order     time.Time
}

// stageKind maps an event stage to a timeline kind.
func stageKind(stage string) string {
	switch {
	case stage == "invoice" || stage == "draft" || stage == "invoice_failed":
		return "invoice"
	case strings.HasPrefix(stage, "client_") || stage == "manual_reminder":
		return "reminder"
	case stage == "final_notice":
		return "notice"
	case strings.HasPrefix(stage, "admin_"):
		return "alert"
	case stage == "suspend" || stage == "suspend_now":
		return "suspend"
	case stage == "unsuspend" || stage == "unsuspend_alert":
		return "unsuspend"
	case stage == "provider":
		return "provider"
	case strings.HasPrefix(stage, "domain_"):
		return "domain"
	}
	return "action"
}

// StageTitle is an event stage in words.
func StageTitle(e Event) string {
	s := e.Stage
	switch s {
	case "created":
		return "Subscription created"
	case "invoice":
		return "Invoice emailed to the client"
	case "draft":
		return "Draft invoice ready to send (auto-send is off)"
	case "invoice_failed":
		return "The invoice could not be created"
	case "final_notice":
		return "Final notice to the client: service will be suspended"
	case "suspend":
		if e.Audience == ToClient {
			return "Client told the service is suspended"
		}
		return "Service suspended"
	case "suspend_now":
		return "Suspended by an administrator"
	case "unsuspend":
		if e.Audience == ToClient {
			return "Client told the service is active again"
		}
		return "Service unsuspended"
	case "unsuspend_alert":
		return "Admins told it's active again"
	case "provider":
		return "At the provider"
	case "extend":
		return "More time given"
	case "skip":
		return "Won't be suspended for this invoice"
	case "unskip":
		return "Suspension back on for this invoice"
	case "mark_paid":
		return "Marked as paid"
	case "manual_reminder":
		return "Reminder sent by hand"
	case "cancel_scheduled":
		return "Cancellation scheduled for the end of the term"
	case "cancelled":
		return "Cancelled by an administrator"
	case "ended":
		return "Ended at the end of the term"
	}
	var n int
	if _, err := fmt.Sscanf(s, "client_%d", &n); err == nil {
		return StepLabel(Step{Kind: KindReminder, Offset: n})
	}
	if _, err := fmt.Sscanf(s, "admin_%d", &n); err == nil {
		return StepLabel(Step{Kind: KindAlert, Before: n})
	}
	if _, err := fmt.Sscanf(s, "domain_%d", &n); err == nil {
		return fmt.Sprintf("Admin reminder: domain expires in %d days", n)
	}
	if s == "domain_expired" {
		return "Admin alert: domain expired"
	}
	return strings.ReplaceAll(s, "_", " ")
}

func (s *Service) timeline(org string, sub *Subscription, pers []BillingPeriod, evs []Event, tasks []Task, today time.Time) []TimelineItem {
	out := []TimelineItem{}
	byPeriod := map[string]BillingPeriod{}
	for _, p := range pers {
		byPeriod[p.ID] = p
		if p.InvoiceID == "" {
			continue
		}
		out = append(out, TimelineItem{Date: DayStr(p.IssueDate), Kind: "invoice", Title: fmt.Sprintf("Invoice %s created", p.InvoiceNumber), InvoiceID: p.InvoiceID,
			Detail: fmt.Sprintf("%s · %s · due %s", periodText(Span{Start: p.Start, End: p.End}), money(p.Amount, p.Currency), longDate(p.DueDate)), Status: "done", order: p.CreatedAt})
		switch {
		case p.Status == PerPaid && p.PaidAt != nil:
			out = append(out, TimelineItem{Date: DayStr(LocalDay(*p.PaidAt, s.Loc(org))), Kind: "paid", Title: fmt.Sprintf("Invoice %s paid", p.InvoiceNumber), InvoiceID: p.InvoiceID,
				Detail: money(p.Amount, p.Currency), Status: "done", order: *p.PaidAt})
		case p.Status == PerVoid:
			out = append(out, TimelineItem{Date: DayStr(Day(p.UpdatedAt)), Kind: "invoice", Title: fmt.Sprintf("Invoice %s voided", p.InvoiceNumber), InvoiceID: p.InvoiceID, Status: "done", order: p.UpdatedAt})
		}
	}
	for _, e := range evs {
		if e.Stage == "digest" || e.Status == "sending" {
			continue
		}
		it := TimelineItem{Date: DayStr(e.Day), Kind: stageKind(e.Stage), Audience: e.Audience, Title: StageTitle(e), Detail: e.Subject, Status: e.Status, order: e.CreatedAt}
		if e.Stage == "provider" {
			it.Title = e.Subject
			it.Detail = ""
		}
		if strings.EqualFold(strings.TrimSpace(it.Detail), strings.TrimSpace(it.Title)) {
			it.Detail = ""
		}
		if e.Recipients != "" && (e.Status == "sent" || e.Status == "failed") {
			it.Detail = strings.TrimSpace(it.Detail + " → " + e.Recipients)
		}
		if e.Error != "" {
			it.Detail = strings.TrimSpace(it.Detail + " — " + e.Error)
		}
		if p, ok := byPeriod[e.PeriodID]; ok {
			it.InvoiceID = p.InvoiceID
		}
		out = append(out, it)
	}
	for _, t := range tasks {
		if t.Status != TaskOpen {
			continue
		}
		out = append(out, TimelineItem{Date: DayStr(t.DueDate), Kind: "task", Audience: ToAdmin, Title: t.Title, Detail: t.Note, Status: "open", order: t.CreatedAt})
	}
	// The plan for the unpaid invoice.
	if sub.Open != nil && sub.Status != StatusCancelled && sub.Status != StatusSuspended && sub.Open.Sent {
		var per BillingPeriod
		if s.DB.Where("id = ?", sub.Open.PeriodID).First(&per).Error == nil {
			st := s.SettingsFor(org)
			d := s.datesFor(sub, &per, st, s.warnedFor(per.ID), today)
			done := map[string]bool{}
			for _, e := range evs {
				if e.PeriodID == per.ID {
					done[e.Key] = true
				}
			}
			for _, step := range PolicyOf(st).Steps(d) {
				if step.Date.Before(today) || done[step.EventKey(per.ID, d)] {
					continue
				}
				it := TimelineItem{Date: DayStr(step.Date), Kind: map[string]string{KindReminder: "reminder", KindAlert: "alert", KindFinalNotice: "notice", KindSuspend: "suspend"}[step.Kind],
					Audience: step.Audience, Title: StepLabel(step), Status: "planned", InvoiceID: per.InvoiceID, order: step.Date.Add(23 * time.Hour)}
				if step.Kind == KindSuspend {
					it.Title = "Service suspended unless paid"
					if d.Deferred {
						it.Detail = fmt.Sprintf("Moved from %s so the admins hear about it at least %d days ahead", longDate(*d.Planned), MinNoticeDays)
					}
				}
				out = append(out, it)
			}
		}
	}
	if sub.Status != StatusCancelled && sub.NextInvoiceOn != "" {
		if d, ok := ParseDay(sub.NextInvoiceOn); ok && !d.Before(today) {
			out = append(out, TimelineItem{Date: sub.NextInvoiceOn, Kind: "next", Title: "Next invoice", Detail: "for the period from " + longDate(Day(sub.NextPeriodStart)) +
				", due " + sub.NextDueDate, Status: "planned", order: d.Add(23 * time.Hour)})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Date != out[j].Date {
			return out[i].Date < out[j].Date
		}
		return out[i].order.Before(out[j].order)
	})
	return out
}
